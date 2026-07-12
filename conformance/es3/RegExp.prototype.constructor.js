function check(a,b){if(a!==b)throw a+" !== "+b;} check(/abc/.constructor,RegExp);check(RegExp.prototype.constructor,RegExp);console.log('PASS');
