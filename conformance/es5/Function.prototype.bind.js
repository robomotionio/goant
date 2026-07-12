function check(a,b){if(a!==b)throw a+" !== "+b;} check(Function.prototype.bind!==undefined,true);var f=function(a,b){return this.x+a+b;};var b=f.bind({x:1},2);check(b(3),6);console.log('PASS');
