function check(a,b){if(a!==b)throw a+" !== "+b;} check(Function.prototype.call.call(function(){return this.x;},{x:5}),5);console.log('PASS');
