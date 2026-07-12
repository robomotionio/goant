function check(a,b){if(a!==b)throw a+" !== "+b;} function F(){}var f=new F();check(F.prototype.isPrototypeOf(f),true);check(Object.prototype.isPrototypeOf(f),true);console.log('PASS');
